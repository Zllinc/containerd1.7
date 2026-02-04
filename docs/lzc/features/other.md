1.1	研究意义
行人重识别是一项图像检索任务，即给定一张行人图像，从图库中返回相似度最高的前N张行人图像。返回图像附带有拍摄时间以及拍摄摄像头这些元信息，根据这些信息，就能够生成行人轨迹，可以用于追踪犯罪嫌疑人；也能够定位到图像所属的那段视频，可以用于搜集证据。所以，行人重识别能在安保领域发挥重要作用。
犯罪案件多发生于夜晚，而且犯罪分子往往会在白天进行踩点、收集信息，晚上作案。针对这些情况，需要准确的白天-黑夜行人重识别系统。大多数摄像头在白天拍摄可见光频段的视频，在夜晚，由于能见度较低，则切换成红外模式，拍摄红外视频。因此，白天-黑夜行人重识别任务就变成了可见光-红外光跨模态行人重识别任务。

图1，可见光图像和红外光图像
如图1所示，可见光图像和红外光图像有着显著的差异，相较于可见光图像，红外光图像是灰度图像，细节更少，噪声更多。普通的行人重识别模型都是针对可见光图像设计的，因此，将这些模型应用于可见光-红外光跨模态行人重识别任务时，效果并不理想。
所以，为了更好的打击犯罪，需要专门为可见光-红外光跨模态行人重识别任务设计模型。基于深度学习的单模态行人重识别近来取得巨大的成功，在Market1501数据集上的准确率甚至超过人类。而且深度学习社区非常繁荣，有足够多的工具包可用，也有专门的为深度学习设计的加速计算硬件。所以，为了能高效率、高质量的处理白天-黑夜行人重识别任务，需要研究基于深度学习的可见光-红外光跨模态行人重识别模型。
1.2 国内外研究现状
单模态行人重识别要面对的问题，可见光-红外光跨模态行人重识别也要面对。所以，本节将先介绍单模态行人重识别的研究现状，再介绍可见光-红外光跨模态行人重识别的研究现状。
1.2.1	单模态行人重识别
行人重识别的基本解决方案是，将要检索的行人图像以及图库中的行人图像，一一映射成一维向量。再根据图库图像和检索图像向量间的余弦距离或欧几里得距离，将图库图像由小到大排序。距离越小，和检索图像间的相似度就越大。排好序的图像列表即为行人重识别的结果。
为了提升行人重识别模型的效果，大多数工作从增强模型的特征提取能力和设计损失函数这两方面着手。
K.Yang et al. [1] 认为ReID既需要衣服和裤子这些大尺度特征，也需要鞋子和眼镜这些小尺度特征。所以他们设计了新的网络构造块来构建新的特征提取网络OSNet。OSNet从多个大小不同的感受域中提取全方位的特征，而普通的构造块只能从单一感受域中提取单一尺度的特征。由于摄像头的成像质量不同，拍摄角度不同等，不同摄像头拍摄出来的图像存在一定的风格差异。X.Pan et al. [2] 因此设计了IBN-Net。将可以减少风格差异的Instance Normalization融合进普通的构造块之中，从而增强模型的特征提取能力。然而，这些重新设计网络基本构造块的方法，需要在百万级图像分类数据集上进行预训练，训练成本比较高。
抑制无关信息，增强关键特征的注意力机制，也常常被使用。最典型的是Non-local block [3]。对于特征图中的每个像素点，计算其与其他像素点间的内积，然后使用softmax归一化，生成注意力图，然后加权平均，生成该像素新的特征。不少的研究是基于修改Non-Local block的。比如SONL [4]，修改了注意力矩阵的计算方式，计算二阶注意力图；ABD-Net [5]，除了使用基于位置的Non-Local block以外，还使用了基于通道的Non-Local block，增强关键通道的表示能力，抑制无关通道的特征；Relation Network [6] 则将继续对注意力矩阵进行卷积操作，获取“特征关系”的特征。虽然注意力机制可以和已有的网络架构结合使用，但相较于卷积层，注意力机制需要较高的显存和算力，对硬件设备的要求较高。
普通的模型直接使用平均池化方法来生成图像一维特征表示向量，这种方法丢失了特征的位置信息。为了保留特征的位置信息，研究者们设计了提取局部特征的方法。PCB [7] 模型将特征图划分成6等分，然后再分别进行平均池化，这样就生成了6个一维特征表示向量，每个向量代表图像相应位置的特征，在计算相似度时，将6个向量串接在一起，形成1个一维向量。有不少研究研究者对局部特征方法进行跟进研究，如图表7所示，相较于将图像划分成6等分，MGN [8] 将划分成1张全图，2张1/2图，3张1/3图。经测试，这种划分方法效果更好。AlignedReID [9] 的作者认为，局部特征的内容没有对齐，因此设计了特征对齐算法，对齐两张图像的特征。
除了增强模型的特征提取能力以外，使用适当的损失函数也很关键。行人重识别的有两类损失函数，一种是pairwise loss，另一种是proxy loss。
因为行人重识别的目标是计算两张图像的相似度，所以pairwise loss就直接以两张图像间的相似度为训练目标。训练中每次迭代如果包含N张图像，那么就有大小为NxN的目标相似度矩阵。然而，相似度矩阵中，不相似的图像（负例）比相似的图像（正例）多得多，如果直接目标相似度矩阵为训练目标，则模型会偏向于负例，即大多预测都是不相似。为此，pairwise loss需要解决正负样例不平衡问题。最常用的方法是Triplet Loss [10]。Triplet loss进行两次筛选，一是选择最难的正样例（相似度最低）和最难的负样例（相似度最高）；二是所选择的样例之中，要求正样例和负样例之差小于某个阈值，才把这两个样例计入最终的loss。第一次筛选保证了正负样例平衡，第二次筛选则专注于困难样本，不再把已经处理得很好的样例纳入优化范围。除了Triplet loss以外，研究者们还设计了根据样例难度动态调整样例权重的Circle loss [11]；不同的样例选择方式N-Pair Loss [12] 等等。
因为行人重识别的训练集和测试集所包含的行人ID不同，如训练集包含ID为1~700的行人图像，测试集包含ID为701~1500的行人图像，在训练是直接的行人ID进行分类，所获得的分类结果，在测试时是没有任何用处的。但是，可以把分类中所用到的全连接层中的权重，当成是不同类别的中心，此时，应用分类损失函数就相当于优化每个样例和对应类别中心间的距离，相同类别的样例都靠拢在类别中心，那么相同类别样例之间的距离也就缩短了。因此，分类损失函数和相应的全连接层加一起被称作proxy loss。Proxy loss还有很多其他的种类，如根据优化的度量函数不同，有CosFace [13] [14]、Arcface [15] 和ProxyNCA [16]，分别优化余弦距离、角度距离和欧几里得距离。
1.2.2	可见光-红外光跨模态行人重识别的研究现状
目前可见光-红外光跨模态行人重识别的研究方向主要有两种，一是减少可见光-红外光图像间的模态差异，二是设计适合跨模态的损失函数。
设计多路结构 [17] 是减少模态差异的方法之一。浅层网络抽取的是纹理等细节特征，深层网络抽取的是衣服、裤子等高层次语义特征。因此不同模态分别使用不同的浅层网络去抽取各自细节特征，用同一个深层网络去抽取最终的高层次语义特征，可以达到一个不错的减少模态差异效果。相较于单路网络，多路网络需要更多的参数，使得模型需要更大的空间。
生成对抗网络也可以减少模态差异。AlignGAN [18] 使用生成网络，将可见光图像转换成伪红外光图像，直接在图像层面减少模态差异。再和真正的红外光图像一起输入到单模态行人重识别模型中，得到最终的特征表示向量。除了将见光图像转换成伪红外光图像外，D2RL [19]、Hi-CMD [20] 和JSIA [21] 将可见光-红外光相互转换，每张图像都有红外光向量和可见光向量，串接在一起计算相似度。X-modlity [22] 将两者分别转换到中间模态，再用中间模态去获取特征表示向量。cmGAN [23] 只使用对抗学习，让生成自两个模态的特征不可区分。生成对抗网络的问题是比较难去训练，而且即使是在推断阶段，也需要生成对抗网络参与，使得模型需要更多的算力。
另一个研究方向就是设计适合跨模态的损失函数。如图所示，如果沿用单模态的损失函数，可以发现，尽管同属一个类的样本都比较靠近，但是，不同模态样例之间仍然泾渭分明。因为跨模态行人重识别的目标是提升不同模态间图像的相似度，这种结果是不能接受的。为解决这个问题，HC Loss [24] 提出直接拉近模态中心的损失函数。HC Loss提出直接拉近模态中心的损失函数。即计算出每个类每个模态的几何中心，然后拉近同属一个类，但模态不同的样本中心。BDTR [25] 分别计算同模态间和跨模态间的triplet loss。HPILN [26] 除了计算全局的triplet loss，还计算跨模态的triplet loss。HC-Tri [17]计算模态中心的triplet loss。
参考文献：
[1] Zhou K, Yang Y, Cavallaro A, et al. Omni-scale feature learning for person re-identification[C]//Proceedings of the IEEE/CVF International Conference on Computer Vision. 2019: 3702-3712.
[2] Pan X, Luo P, Shi J, et al. Two at once: Enhancing learning and generalization capacities via ibn-net[C]//Proceedings of the European Conference on Computer Vision (ECCV). 2018: 464-479.
[3] Wang X, Girshick R, Gupta A, et al. Non-local neural networks[C]//Proceedings of the IEEE conference on computer vision and pattern recognition. 2018: 7794-7803.
[4] Xia B N, Gong Y, Zhang Y, et al. Second-order non-local attention networks for person re-identification[C]//Proceedings of the IEEE/CVF International Conference on Computer Vision. 2019: 3760-3769.
[5] Chen T, Ding S, Xie J, et al. Abd-net: Attentive but diverse person re-identification[C]//Proceedings of the IEEE/CVF International Conference on Computer Vision. 2019: 8351-8361.
[6] Zhang Z, Lan C, Zeng W, et al. Relation-aware global attention for person re-identification[C]//Proceedings of the IEEE/CVF Conference on Computer Vision and Pattern Recognition. 2020: 3186-3195.
[7] Sun Y, Zheng L, Yang Y, et al. Beyond part models: Person retrieval with refined part pooling (and a strong convolutional baseline)[C]//Proceedings of the European conference on computer vision (ECCV). 2018: 480-496.
[8] Wang G, Yuan Y, Chen X, et al. Learning discriminative features with multiple granularities for person re-identification[C]//Proceedings of the 26th ACM international conference on Multimedia. 2018: 274-282.
[9] Zhang X, Luo H, Fan X, et al. Alignedreid: Surpassing human-level performance in person re-identification[J]. arXiv preprint arXiv:1711.08184, 2017.
[10] Hermans A, Beyer L, Leibe B. In defense of the triplet loss for person re-identification[J]. arXiv preprint arXiv:1703.07737, 2017.
[11] Sun Y, Cheng C, Zhang Y, et al. Circle loss: A unified perspective of pair similarity optimization[C]//Proceedings of the IEEE/CVF Conference on Computer Vision and Pattern Recognition. 2020: 6398-6407.
[12] Sohn K. Improved deep metric learning with multi-class n-pair loss objective[C]//Proceedings of the 30th International Conference on Neural Information Processing Systems. 2016: 1857-1865.
[13] Wang H, Wang Y, Zhou Z, et al. Cosface: Large margin cosine loss for deep face recognition[C]//Proceedings of the IEEE conference on computer vision and pattern recognition. 2018: 5265-5274.
[14] Liu H, Zhu X, Lei Z, et al. Adaptiveface: Adaptive margin and sampling for face recognition[C]//Proceedings of the IEEE/CVF Conference on Computer Vision and Pattern Recognition. 2019: 11947-11956.
[15] Deng J, Guo J, Xue N, et al. Arcface: Additive angular margin loss for deep face recognition[C]//Proceedings of the IEEE/CVF Conference on Computer Vision and Pattern Recognition. 2019: 4690-4699.
[16] Movshovitz-Attias Y, Toshev A, Leung T K, et al. No fuss distance metric learning using proxies[C]//Proceedings of the IEEE International Conference on Computer Vision. 2017: 360-368.
[17] Liu H, Tan X. Parameters Sharing Exploration and Hetero-Center based Triplet Loss for Visible-Thermal Person Re-Identification[J]. arXiv preprint arXiv:2008.06223, 2020.
[18] Wang G, Zhang T, Cheng J, et al. RGB-infrared cross-modality person re-identification via joint pixel and feature alignment[C]//Proceedings of the IEEE/CVF International Conference on Computer Vision. 2019: 3623-3632.
[19] Wang Z, Wang Z, Zheng Y, et al. Learning to reduce dual-level discrepancy for infrared-visible person re-identification[C]//Proceedings of the IEEE/CVF Conference on Computer Vision and Pattern Recognition. 2019: 618-626.
[20] Choi S, Lee S, Kim Y, et al. HI-CMD: hierarchical cross-modality disentanglement for visible-infrared person re-identification[C]//Proceedings of the IEEE/CVF Conference on Computer Vision and Pattern Recognition. 2020: 10257-10266.
[21] Wang G A, Zhang T, Yang Y, et al. Cross-modality paired-images generation for RGB-infrared person re-identification[C]//Proceedings of the AAAI Conference on Artificial Intelligence. 2020, 34(07): 12144-12151.
[22] Li D, Wei X, Hong X, et al. Infrared-visible cross-modal person re-identification with an x modality[C]//Proceedings of the AAAI Conference on Artificial Intelligence. 2020, 34(04): 4610-4617.
[23] Dai P, Ji R, Wang H, et al. Cross-modality person re-identification with generative adversarial training[C]//IJCAI. 2018, 1: 2.
[24] Zhu Y, Yang Z, Wang L, et al. Hetero-center loss for cross-modality person re-identification[J]. Neurocomputing, 2020, 386: 97-109.
[25] Ye M, Lan X, Wang Z, et al. Bi-directional center-constrained top-ranking for visible thermal person re-identification[J]. IEEE Transactions on Information Forensics and Security, 2019, 15: 407-419.
[26] Zhao Y B, Lin J W, Xuan Q, et al. HPILN: a feature learning framework for cross-modality person re-identification[J]. IET Image Processing, 2019, 13(14): 2897-2904.


二、研究内容（说明课题的具体内容，独创及新颖之处，重点解决的问题，预期的研究成果）：

2.1 具体研究内容
a)设计一个模态相关Normalization层来减少模态差异。Batch Normalization是神经网络中常用的一种构造块，一般在每一个卷积层后面都会接一个BN层。BN层可以加速网络训练以及提升网络的泛化能力。对于跨模态行人重识别任务，我们发现普通的BN层会导致模态分布差异。BN层有两个步骤，先对输入特征进行归一化，即把输入特征的均值变成0，方差变成1，然后在进行线性变化。因为不同模态间的分布存在差异，所以，对于BN层的第一步，虽然整体的均值变成0，方差变成1，但是对于可见光子集和红外光子集而言，这两个子集的均值均不是0，方差均不是1，并不符合Batch Normalization的思想。为此，我们打算分别对可见光子集和红外光子集分别进行归一化，以让其符合Batch Normalization的思想。我们把这种新设计的层称作模态相关Normalization层(MBN)。
b)设计适合跨模态行人重识别的损失函数。我们发现现有的跨模态损失函数存在两个问题，第一是损失函数进行的难样本选择，有可能某个模态的样本选择得多一点，另一个模态的样本选择得少一点，这会导致模态优化不平衡。即模型集中优化某个模态而忽略另一个模态，最终导致跨模态行人重识别的效果不好。为了解决这个问题，我们打算不进行难样本选择，而是设计一种根据样本难度动态调整权重的损失函数，从而保证各个模态优化平衡。第二是现有的模型使用了多个损失函数，然而，这些损失函数却没有一同优化同一个度量函数，有的优化欧几里得距离，有的优化余弦距离。更糟糕的是，训练时优化的距离和测试时使用的距离并不相同。所以，在损失函数的选择上，打算让所使用的损失函数都优化同一个度量函数。
c)设计针对跨模态行人重识别的动态匹配新架构。普通的行人重识别模型没有办法进行动态匹配。即，如果要检索的图像是黑白图像，那么就将图库中图像的颜色特征去掉，如特征如果是蓝色的衬衫，那就变成衬衫。然而，动态匹配是跨模态行人重识别所非常需要的。为此，我们打算使用动态神经网络，来实现动态匹配，作为跨模态行人重识别的新架构。

2.2 创新点
a)MBN设计的新颖之处在于，不使用生成网络去解决模态差异问题，直接在原神经网络结构上微调即可，可以大大减少计算量。
b)不使用难样本选择的损失函数，缓解了普通损失函数在跨模态情况下会出现的模态优化不平衡问题。
c) 动态匹配架构改变了行人重识别的基本框架，赋予行人重识别模型根据查询图像不同而动态生成特征的动态匹配的能力。

2.3 重点解决的问题
a)如何将MBN融入到常用的网络结构之中。
b)如何设计可以根据样本难度动态调整权重的函数。
c)如何选择基本的动态神经网络结构以及针对性的设计适合行人重识别的动态神经网络。
2.4 预期的研究成果
a)设计出一种使用MBN层的神经网络结构，使用这种结构可以大幅减少模态差异。
b)设计出一种可以根据样本难度动态调整权重的损失函数，不会造成模态优化不平衡问题，并且可以增强模型效果。
c)设计出一个可以进行动态匹配的跨模态行人重识别模型，可以根据查询图像不同而动态生成特征，并且运行效率比肩普通的行人重识别模型。


三、科研方案设计（包括：研究方法、技术路线、理论分析、计算、实验方法和步骤及其可行性，可能出现的技术问题及解决方法）：

3.1 研究路线
1) 分析现有模型
分析不同Normalization层对模态分布差异的影响；分析现有损失函数关于不同模态样本以及不同难度样本的梯度大小；分析现有模型的特征抽取能力，即可以抽取哪些尺度的特征、哪些特征对模型的效果影响最大等等。
2) 针对性设计模块
根据分析的结果，选择可以减少模态分布差异的Normalization层作为网络的Normalization层；根据现有损失函数出现的梯度问题，设计具有更好梯度性质的损失函数；根据现有模型的特征提取能力，设计相应的动态神经网络。
3）比较不同模块效果
将设计的模块应用到不同的架构之中，比较应用前后的效果，看是否都有提升；调节所设计模块的超参数，比较不同参数对模块效果的影响；和其他研究者设计的相似的模块相比，看是否有优越性。

3.2 可能出现的技术问题
1）所设计的模块应该应用到网络的哪个位置、使用的超参数应该是多少，才能有最优效果？
2）设计出来的模块可能不相互兼容，一起使用时效果不如单独使用任意一个好。
3）新的模块可能会增加模型的时间和空间复杂度。

3.3 解决方法
1）针对网络结构选择问题，可以使用神经网络搜索技术；针对超参数选择问题，可以使用基于贝叶斯估计的最优化方法。
2）根据具体的使用情景，选择最佳的模块组合。如果可以，针对组合使用的情况，优化各个模块的设计。
3）尽可能的优化模块设计，使得所需的时间和空间复杂度最小；根据具体的硬件情况，对各模块进行相应裁剪，以保证模型可以正常运行。